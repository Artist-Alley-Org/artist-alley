// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package brushpacks

import (
	"errors"
	"testing"
)

func TestNilStr(t *testing.T) {
	if nilStr("") != nil {
		t.Error("nilStr(\"\") should be nil")
	}
	got := nilStr("hi")
	if got == nil {
		t.Fatal("nilStr(\"hi\") should not be nil")
	}
	if *got != "hi" {
		t.Errorf("got %q, want hi", *got)
	}
}

// isNoRows compares the wrapped error's message because pgx wraps
// the sentinel into a typed error chain that doesn't survive a
// straightforward errors.Is. The literal-string comparison must
// match pgx's exact wording — if pgx ever changes it, this test
// surfaces the drift loudly instead of silently turning every
// not-found into a 500.
func TestIsNoRows(t *testing.T) {
	if isNoRows(nil) {
		t.Error("isNoRows(nil) = true, want false")
	}
	if !isNoRows(errors.New("no rows in result set")) {
		t.Error("isNoRows(literal pgx message) = false, want true")
	}
	if isNoRows(errors.New("connection refused")) {
		t.Error("isNoRows should only match the pgx no-rows message")
	}
}

// The two sentinel errors are exported for the handler to recognise
// — make sure they're distinct so callers can errors.Is against
// each one independently.
func TestSentinelErrors_Distinct(t *testing.T) {
	if errors.Is(ErrPackNotFound, ErrStampNotFound) {
		t.Error("ErrPackNotFound and ErrStampNotFound should not match each other")
	}
	if !errors.Is(ErrPackNotFound, ErrPackNotFound) {
		t.Error("ErrPackNotFound should match itself")
	}
}
