// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"errors"
	"testing"
	"time"
)

func TestProviderError_FormatsPrefix(t *testing.T) {
	pe := &ProviderError{
		Class:    ErrClassTransient,
		Provider: "openai",
		Model:    "gpt-4o",
		Wrapped:  errors.New("connection refused"),
	}
	got := pe.Error()
	want := "ai: openai/gpt-4o: connection refused"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestProviderError_FormatsClassNameWhenNoWrappedErr(t *testing.T) {
	pe := &ProviderError{Class: ErrClassRateLimit, Provider: "claude", Model: "opus"}
	got := pe.Error()
	want := "ai: claude/opus: rate_limited"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestProviderError_IsRetryable(t *testing.T) {
	cases := []struct {
		class ErrorClass
		want  bool
	}{
		{ErrClassTransient, true},
		{ErrClassRateLimit, true},
		{ErrClassPermanent, false},
		{ErrClassBudget, false},
		{ErrClassPrivacy, false},
	}
	for _, c := range cases {
		pe := &ProviderError{Class: c.class}
		if got := pe.IsRetryable(); got != c.want {
			t.Errorf("class=%s IsRetryable() = %t, want %t", classString(c.class), got, c.want)
		}
	}
}

func TestProviderError_IsRetryable_NilSafe(t *testing.T) {
	var pe *ProviderError
	if pe.IsRetryable() {
		t.Error("nil ProviderError.IsRetryable() = true, want false")
	}
}

func TestAsProviderError_Found(t *testing.T) {
	pe := &ProviderError{Class: ErrClassPermanent, Provider: "gemini"}
	got, ok := AsProviderError(pe)
	if !ok || got != pe {
		t.Errorf("AsProviderError returned (%v, %t)", got, ok)
	}
}

func TestAsProviderError_NotFound(t *testing.T) {
	plain := errors.New("nope")
	if _, ok := AsProviderError(plain); ok {
		t.Error("AsProviderError(plain) = ok, want false")
	}
}

func TestAsProviderError_WrappedChain(t *testing.T) {
	pe := &ProviderError{Class: ErrClassTransient, Provider: "ollama"}
	wrapped := errors.Join(errors.New("ctx"), pe)
	got, ok := AsProviderError(wrapped)
	if !ok || got != pe {
		t.Errorf("AsProviderError(wrapped) = (%v, %t)", got, ok)
	}
}

func TestErrNoProviderAvailable_Sentinel(t *testing.T) {
	if ErrNoProviderAvailable == nil {
		t.Fatal("sentinel is nil")
	}
	if !errors.Is(ErrNoProviderAvailable, ErrNoProviderAvailable) {
		t.Fatal("errors.Is should match itself")
	}
}

// Compile-time sanity checks: confirm Content / Message / Tool decl
// shapes round-trip without hidden alignment surprises.
func TestMessage_ShapeRoundTrips(t *testing.T) {
	m := Message{
		Role: "user",
		Content: []Content{
			{Type: ContentTypeText, Text: "hello"},
			{Type: ContentTypeImageURL, ImageURL: "https://example.com/x.png"},
		},
	}
	if len(m.Content) != 2 {
		t.Fatalf("content len = %d", len(m.Content))
	}
	if m.Content[1].Type != ContentTypeImageURL {
		t.Errorf("content[1].Type = %q", m.Content[1].Type)
	}
}

func TestCompletionResponse_DurationFieldSurvives(t *testing.T) {
	r := CompletionResponse{Text: "ok", Duration: 250 * time.Millisecond}
	if r.Duration != 250*time.Millisecond {
		t.Errorf("Duration = %v", r.Duration)
	}
}
